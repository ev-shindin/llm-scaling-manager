# Cap what each tenant may take

## Start here

- **Status:** Stable; no end-to-end spec yet bounds the default optimizer by a quota ([How it is tested](#how-it-is-tested)).
- **You need:**
  - [the saturation path](../scale-on-saturation/#start-here) in place; the accelerator names the controller logs (a quota key must match one exactly).
  - a cluster admin, for a budget the tenant cannot edit ([Who owns what](#who-owns-what)).
  - on **Kubernetes**, for the cluster policy to reach a namespace-scoped controller running in the namespace it manages: `get` on its Namespace, which `make setup-prereqs` grants. One set up before that grant existed keeps reading its own ConfigMap — the tenant's — and says so (`Cannot tell whether a cluster admin published policy` in its log, `wva_policy_namespace_unverified` = 1, alert `WVAPolicyNamespaceUnverified`) until a cluster admin re-runs `make setup-prereqs` ([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-to-this-release)).
  - **Not enforcement:** the limiter is advisory; back each tenant namespace with a GPU `ResourceQuota` ([bound at admission](../../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).
- **Do this** (the [admin guide](../../guides/admin-gpu-bounding/) for the cluster policy; the steps are on this page for one install):
  1. Check accelerators resolve ([how](../../guides/admin-gpu-bounding/README.md#prerequisites)). Namespace owner: `kubectl get events -n <ns> --field-selector reason=AcceleratorNotResolved`; cluster admin, every namespace at once: the same with `-A`. No events is the healthy answer.
  2. Declare the quota, one of:
     - Namespace owner (advisory; you can edit it): `WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=<n>' make deploy-wva` ([configuration](../../reference/configuration.md#core-configuration)).
     - Cluster admin (a budget the tenant cannot edit): `make enable-physical-limiter WVA_LIMITER_TYPE=quota WVA_QUOTAS='<accelerator>=<n>'` ([admin guide](../../guides/admin-gpu-bounding/README.md#configuration)).
  3. Several namespaces named one by one: hand-write the entry ([Declaring it](#declaring-it)) into the `limiters:` list of the `default` entry in `wva-scaling-policy-config` — in `wva-policy` for the cluster policy (cluster admin), or in the controller's own namespace for one install.
  Re-running `make deploy-wva` resets the `default` entry, limiter included: pass `WVA_LIMITER` and `WVA_QUOTAS` again on every re-run, and re-add a hand-written entry afterwards ([why](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).
- **Check it works:** `kubectl get cm wva-controller-status -n <wva-namespace> -o jsonpath='{.data.policyNamespace} {.data.limiter} {.data.policyAccepted}'` names where the budget is read from and shows `quota/<name>`. In the log, the controller logs `GPU limiter (re)built from config` with `type` `quota` (after a restart: `GPU limiter constructed` with `type` `quota`; that line alone is printed by every controller, `type` `none` included). Then a model held at its allowance raises `wva_decisions_limited_total` or a `ResourceConstrained` Event. A **fully spent** quota emits neither: compare `kubectl get pods` against the quota ([Verifying it](#verifying-it)).
- **Undo:**
  - Cluster policy (cluster admin): `make disable-physical-limiter WVA_LIMITER_TYPE=quota` ([Cleanup](../../guides/admin-gpu-bounding/README.md#cleanup)). Without `WVA_LIMITER_TYPE=quota` it removes only the physical limiter and leaves the quota in force. It also clears the limiter it wrote into admin-owned controllers' own ConfigMaps, and leaves the read-only RBAC it granted in place.
  - One install (namespace owner): re-run `make deploy-wva` without `WVA_LIMITER`; the installer re-applies the shipped `default` entry, which declares no limiter.

## What it is

Declare how many GPUs of each accelerator type the scaling manager may hold — for the whole
cluster, or per namespace — so one team's autoscaling cannot spend the fleet.
This is the other half of [bounding a fleet by real GPUs](../bound-by-gpus/):
that path asks whether the hardware exists, this one asks whether the tenant is
allowed it.

**Use it when** several teams scale on shared accelerators and somebody has
promised each of them a number, or when you want a ceiling per GPU type that
sits *below* what the cluster physically has.

**Do not use it to stop a workload from taking GPUs.** A quota bounds **the scaling manager**,
and only the scaling manager — it is an allowance granted to the autoscaler, not an admission
rule. The thing that stops any Pod is a Kubernetes `ResourceQuota`, and on a
cluster where unmanaged workloads compete for the same accelerators you want both.

## Who owns what

A quota only bounds a tenant if the tenant cannot edit it. Which ConfigMap the
controller takes it from depends on where the controller runs:

| setting | read from | owned by |
| --- | --- | --- |
| `limiters:` and `optimizer:` (the budget) | the policy namespace, when one resolves for this controller (`wva-policy`, or the namespace a `wva.llmd.ai/policy-namespace` label names) | the cluster admin |
| `limiters:` and `optimizer:`, when no policy namespace resolves | the controller's own `wva-scaling-policy-config` | whoever owns the controller's namespace: the admin for a cluster-scoped or out-of-reach controller, **the tenant** for one running in their own namespace |
| thresholds, tiers, per-model entries, utilization-share weights | a `wva-scaling-policy-config` in the **managed** namespace, if there is one (read as a namespace-local map, in every shape); otherwise the controller's own ConfigMap | the namespace's owner — so in the out-of-reach shape the tenant still sets these for their models, though never the budget |

A policy namespace resolves only for a controller that manages its own namespace
**and** can read Namespace objects. On Kubernetes a namespace-scoped controller
gets that read from `make setup-prereqs`; one set up before the grant existed
cannot, so its budget is its own ConfigMap — the tenant's — and it reports
`wva_policy_namespace_unverified` = 1 until a cluster admin re-runs
`make setup-prereqs` ([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-to-this-release);
[by hand](../../guides/admin-cluster-setup/README.md#grant-the-namespace-read-by-hand)
only where that cannot run).
`WVA_LIMITER=quota` writes that ConfigMap; `WVA_LIMITER_TYPE=quota` with
`make enable-physical-limiter` writes the policy namespace, and the own ConfigMap
of every admin-owned controller it grants, and restarts each namespace-scoped
controller that reads its policy outside the namespace it manages. Where the budget must hold against the tenant,
run the controller
[out of their reach](../../guides/admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach)
and back it with a `ResourceQuota`.

## What it needs

- Cluster-admin rights to publish the policy where a tenant cannot edit it, and
  accelerators that resolve — the same prerequisites as
  [bound-by-gpus](../bound-by-gpus/), which checks them first.
- A decision about scope. One entry is tied to exactly one scope; a deployment
  that needs both caps declares two entries, and the chain consults both.

## Declaring it

Both shapes live in the `limiters:` list on the scaling policy's `default`
entry. The installer generates one for you rather than having you write it —
`WVA_LIMITER=quota WVA_QUOTAS='H200=8'` (one install's own policy) or
`WVA_LIMITER_TYPE=quota WVA_QUOTAS='H200=8'` (the cluster policy every
controller reads), with `WVA_QUOTA_SCOPE=namespace|cluster` choosing between the
two shapes below. Which namespace the generated entry is keyed on follows the
install: a **namespace-scoped** install keys it on the namespace the controller
manages, a **cluster-scoped** one on the reserved `default` key, whose meaning
is the paragraph after next. Hand-write the entry when you want several
namespaces named explicitly; `WVA_QUOTAS` writes one.

**Cluster scope** caps a type across every namespace **one controller
manages**. It is per controller: N controllers each enforce their own copy of
the entry, so a cluster running N of them permits N times the figure.

```yaml
limiters:
  - name: cluster-quota
    type: quota
    scope: cluster
    quotas:
      H100: 16
      A100: 8
      L40S: -1          # unlimited
```

**Namespace scope** caps each tenant, with an optional exclusion list and a
fallback for namespaces you did not list:

```yaml
limiters:
  - name: namespace-quota
    type: quota
    scope: namespace
    exclude: [kube-system, llm-d-system]
    namespaceQuotas:
      team-a:
        H100: 8
      team-priority:
        H100: -1        # unlimited for this tenant
      default:
        H100: 2         # fallback for any namespace not listed
```

`default` there is a **reserved key**, not the Kubernetes namespace of that
name: it selects the fallback, which means the real `default` namespace cannot
be given a quota by listing it. Exclude it, or omit the reserved key entirely so
that anything unlisted is denied.

## Three things that surprise people

**A quota does not bound the cluster.** It counts only what the scaling manager's own variants
hold, so it can hand out capacity that unmanaged workloads have already taken.
Charging it the physical figure would be worse: a namespace with a 4-GPU
allowance beside an unrelated 4-GPU training job would read as fully spent while
the scaling manager had placed nothing, and every scale-up would be refused against an untouched
allowance. "The hardware is full" is a different statement, made by the physical
limiter.

**Ceilings, not reservations.** A finite cap guarantees a tenant never *exceeds*
it — never that it can *reach* it. The cluster aggregate is the sum of the finite
caps, so a namespace that is unlimited (`-1`) or excluded draws from that shared
aggregate without contributing to it, and can consume budget a capped peer would
otherwise have used. Nobody exceeds their own authorization; it is still a
fairness gap, and worth knowing before you promise a team its number.

**A missing entry denies.** `0` or no entry for an accelerator type means no
allocation at all — `-1` is what means unbounded. A typo in a type name is
therefore a denial: the safe direction, and a confusing one.

Fairness is per **model**, not per namespace. Two models in one namespace each
get their own fair-share slot; the cap bounds each of them and their running sum
rather than pooling one model's allowance for the other. The quota clamps *after*
the fair-share split is computed — a ceiling on the outcome, not an input to it.

## Verifying it

As a tenant, start from what you can read — the controller's log. Where the
controller runs depends on the shape: in your namespace if you installed it
there; in the admin's namespace (for example `wva-<your-namespace>`) in the
out-of-reach shape, where you can read its log only if the admin grants
`pods/log` there — otherwise ask them for these two lines.

```bash
# <controller-namespace>: yours in the tenant-owned shape, the admin's in the out-of-reach one
# which policy governs this controller, read once at startup
kubectl logs -n <controller-namespace> deploy/wva-controller-manager \
  | grep -E 'Cluster policy comes from|Could not read the managed Namespace'

# the limiter it built from that policy, and its name: want type=quota.
# "constructed" is printed at every start, type=none included; a live
# change to the policy logs "(re)built from config".
kubectl logs -n <controller-namespace> deploy/wva-controller-manager \
  | grep -E 'GPU limiter (constructed|\(re\)built from config)'
```

No `Cluster policy comes from` line at the default verbosity (that branch logs
only at V(1)) means the budget is in the controller's own
`wva-scaling-policy-config` — yours in the tenant-owned shape, the admin's in
the out-of-reach one. `... the well-known
namespace` (or `... labelled with`) names a policy namespace you normally cannot
read: ask the admin for `get` on `wva-scaling-policy-config` there, or for the
figures. Then, in Prometheus — a decision the limiter clamped (labels
`variant_name`, `limiter_name`; the model's namespace arrives as
`exported_namespace`):

```text
increase(wva_decisions_limited_total{exported_namespace="<ns>"}[15m])
```

or as an Event on the model's ScaledObject:

```bash
kubectl get events -n <ns> --field-selector reason=ResourceConstrained
```

A clamp is a normal, visible outcome — not an error. `wva_model_scaling_blocked`
is not the signal here: its quota reasons come from the
[utilization-share optimizer](../utilization-share/) only, not from the
default one.

**A fully spent quota is silent.** When the allowance is already used up there
is nothing left to clamp, so neither the counter nor the Event appears. Compare
what runs against the quota directly:

```bash
kubectl get pods -n <ns> -o custom-columns='NAME:.metadata.name,GPUS:.spec.containers[*].resources.limits.nvidia\.com/gpu,PHASE:.status.phase'
```

## What it costs

No accelerators. A cap that binds costs latency: a model held at its allowance
queues what another replica would have served, and that shows up as TTFT. It
also costs the fairness gap above, and the ownership question of who may edit
the policy — see [Who owns what](#who-owns-what).

**Several namespace-scoped controllers under `gpu-inventory` can over-ask
together.** Each computes free capacity from the same nodes and allocates
without seeing the others' claims in the same cycle, so in sum they can order
more than is free; the scheduler then leaves the excess Pending. The installer
says this about side-by-side installs (`deploy/lib/single_install.sh`); it is
inferred from the design, not measured. A per-namespace quota on each, or
separate GPU nodes, removes the overlap.

## How it is tested

- Unit: `internal/config/quota_limiter_test.go` (parsing, scopes, validation,
  the reserved-key rules) and
  `internal/engines/allocation/quota_inventory_test.go` (what the inventory
  reports and charges).
- End-to-end: `test/e2e/warm_pool_quota_test.go` covers a refusal — a pool
  asking for accelerators the quota will not give.
  `test/e2e/kueue_quota_test.go` asserts the effective quota entry the limiter
  logs after merging a Kueue grant into a static cap (one, then three again
  once the LocalQueue is gone) — the entry, not what the optimizer then orders.
  `test/e2e/utilization_share_test.go` runs two models under a spent 4-GPU
  namespace quota and asserts the utilization-share optimizer's transfer (the
  donor's marked pod leaves before the receiver grows) — that optimizer, not
  the default one.
- **No e2e covers the default optimizer's allocation under a static quota.**
  `test/e2e/limiter_test.go` exercises the GPU-inventory limiter only. That is
  the leg to add before treating a tenant cap as something CI protects.

## Tuning it

Every field, the namespace lookup rules, validation and the reload lifecycle:
[the quota limiter reference](../../reference/quota-limiter.md). What the two
usage bases mean and why they are counted differently:
[GPU capacity accounting](../../concepts/gpu-capacity-accounting.md).

If the numbers already live in Kueue, the entry can be bounded by them instead
of repeating them: [Bound tenants by the quotas Kueue already
holds](../kueue-bounded-quotas/) (experimental).
