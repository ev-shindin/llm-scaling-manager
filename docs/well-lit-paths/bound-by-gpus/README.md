# Bound a fleet by real GPUs

## Start here

- **Status:** Stable.
- **You need:**
  - a cluster admin, for the cluster-wide command; controllers installed by [the saturation path](../scale-on-saturation/#start-here); accelerators that resolve.
  - on **Kubernetes**, for a namespace-scoped controller that runs in the namespace it manages: `get` on its Namespace, which `make setup-prereqs` grants (`wva-policy-namespace-reader`). One set up before that grant existed keeps reading its own ConfigMap and says so: `Cannot tell whether a cluster admin published policy` in its log, `wva_policy_namespace_unverified` = 1, alert `WVAPolicyNamespaceUnverified`. A cluster admin re-runs `make setup-prereqs` for it ([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-to-this-release); [by hand](../../guides/admin-cluster-setup/README.md#grant-the-namespace-read-by-hand) only where that cannot run).
  - **Not enforcement:** the limiter is advisory; back each tenant namespace with a GPU `ResourceQuota` ([bound at admission](../../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).
- **Do this** ([Bound every install by real GPUs](../../guides/admin-gpu-bounding/)):
  1. (cluster admin) [Check accelerators resolve](../../guides/admin-gpu-bounding/README.md#prerequisites): the failure with no symptom.
  2. (cluster admin) Run [`make enable-physical-limiter`](../../guides/admin-gpu-bounding/README.md#installation-instructions), reading the list it prints first: it restarts every namespace-scoped controller whose policy lives outside the namespace it manages (marked `WILL BE RESTARTED`), on this run and every later one. Leave a namespace out of `WVA_LIMITER_TARGETS` where that restart is not acceptable now.
  Re-run `make enable-physical-limiter` after any `make deploy-wva` of an admin-owned controller: the installer resets its `default` entry ([why](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).
  One install instead (namespace owner): `WVA_LIMITER=gpu-inventory` on its `make deploy-wva` ([configuration](../../reference/configuration.md#core-configuration)). Advisory: the same owner can edit it away.
- **Check it works:** `kubectl get cm wva-controller-status -n <wva-namespace> -o jsonpath='{.data.policyNamespace} {.data.limiter} {.data.policyAccepted}'` names the policy namespace the controller reads, the limiter in force (`inventory/<name>`, not `none`) and whether the policy was accepted. In the log, each controller logs `GPU limiter (re)built from config` with `type` `inventory` (after a restart: `GPU limiter constructed` with the same `type`), and no `Invalid saturation scaling` ([Verification](../../guides/admin-gpu-bounding/README.md#verification)). Every controller prints `GPU limiter constructed` at start, `type` `none` included, so the line alone proves nothing.
- **Undo:** (cluster admin) `make disable-physical-limiter` ([Cleanup](../../guides/admin-gpu-bounding/README.md#cleanup)); it leaves the node-read RBAC in place. One install (namespace owner): re-run `make deploy-wva` without `WVA_LIMITER`, which resets the policy's `default` entry ([why](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).

## What it is

Without a limiter, each controller scales its workloads up to their
`maxReplicaCount` and nothing checks that against the accelerators that exist.
A limiter gives the optimizer a budget: either the GPU inventory actually
discovered on the cluster, or per-accelerator caps an operator declares.

**Use it when** more than one team scales on the same accelerators, or when
`maxReplicaCount` was set as a safety limit rather than a real ceiling — which
is to say, on any shared cluster.

**Do not mistake it for enforcement.** The scaling manager's limiter is advisory: it stops the controller
*asking* for replicas that cannot be placed. The boundary that actually holds is
a Kubernetes `ResourceQuota`. A limiter without a quota bounds a well-behaved
autoscaler, not the cluster.

## What it needs

- Cluster-admin rights, once: the policy must live where a tenant cannot edit
  it, and every controller meant to follow it must be able to read it.
- Every controller able to **list nodes**. A controller that cannot resolve a
  variant's accelerator gives that variant no budget and then never scales it
  up — a failure with no natural symptom, which is why the guide checks
  `reason=AcceleratorNotResolved` first.
- A decision about which limiter, below.

## Which limiter

They answer different questions, and you can **declare both**: every limiter
in the `limiters:` list is built, and a model must fit all of them. Enabling one
kind keeps the other. Declare both when the allowance can exceed the GPUs that
exist, for example a Kueue quota on a shared cluster.

The shipped policy declares no limiter at all. `gpu-inventory` is only the
default of `WVA_LIMITER_TYPE`, the variable `make enable-physical-limiter` reads.

| | `gpu-inventory` | `quota` |
| --- | --- | --- |
| Question | is the hardware there? | is this tenant within its allowance? |
| Counts | every GPU-holding Pod on the cluster | only what **the scaling manager's own variants** hold |
| Declared | nothing to declare — discovered | per accelerator type, at cluster or namespace scope |

Declaring a quota has consequences an operator has to know before promising a
team a number — what it counts, why a cap is a ceiling and not a reservation,
and why a missing entry denies. Those, and both scope shapes, are in
[capping what each tenant may take](../tenant-gpu-quotas/).

This page is about the other question: not exceeding the hardware that exists.

## Setting it up

[Bound every install by real GPUs](../../guides/admin-gpu-bounding/) — one command,
because the ConfigMap is the easy part and the access it then requires is not.

## Verifying it

```bash
# where each controller reads its policy from: "the well-known namespace" means
# wva-policy; no line (that branch logs only at V(1)) means its own ConfigMap
kubectl logs -n <wva-namespace> deploy/wva-controller-manager | grep -E 'Cluster policy comes from|Could not read the managed Namespace'

# the cluster policy, for the controllers that read wva-policy
kubectl get configmap wva-scaling-policy-config -n wva-policy -o jsonpath='{.data.default}'

# the limiter the controller actually built: type=inventory for gpu-inventory.
# "constructed" is printed at every start, type=none included; a live change
# logs "(re)built from config".
kubectl logs -n <wva-namespace> deploy/wva-controller-manager | grep -E 'GPU limiter (constructed|\(re\)built from config)'

# what the optimizer decided, and the reason code behind each entry
kubectl logs -n <wva-namespace> deploy/wva-controller-manager | grep scaling-decision
```

On Kubernetes, a namespace-scoped controller reads Namespace objects only
through the grant `make setup-prereqs` creates. One set up before that grant
existed does not see `wva-policy` and reads its own ConfigMap instead — the
first command shows it (`Could not read the managed Namespace`), and
`wva_policy_namespace_unverified` reads 1 — until a cluster admin re-runs
`NAMESPACE=<ns> make setup-prereqs`, or `make enable-physical-limiter`, which
also restarts it ([grant it by hand](../../guides/admin-cluster-setup/README.md#grant-the-namespace-read-by-hand)
only where neither can run). Admin-owned controllers read their own ConfigMap by design;
see [the GPU limiter](../../reference/gpu-limiter.md#what-an-admin-sets-and-where).

`wva_model_scaling_blocked` carries the reason as a label. A model that wanted
more and did not get it is a normal, visible outcome here — not an error.

## What it costs

No accelerators. A budget that binds costs latency: a model held at its budget
queues what an extra replica would have served, and that shows up as TTFT, not
as an error. What else it costs is a conversation: a limiter turns "why did we
not scale" from an invisible event into a declared policy somebody owns.

## How it is tested

- End-to-end: `test/e2e/limiter_test.go` (labelled `full`), and
  `test/e2e/warm_pool_quota_test.go` for the refusal path when a pool asks for
  accelerators the quota will not give.
- The accounting itself — and three ways a GPU budget can over-state free
  capacity — is written up in
  [gpu-capacity-accounting.md](../../concepts/gpu-capacity-accounting.md).

## Tuning it

The limiter is selected by the `limiters:` list on the scaling-policy
ConfigMap's `default` entry — that list is the only thing the controller reads,
and because it is dynamic configuration the engine rebuilds it without a
restart (a namespace-scoped controller reading it from outside the namespace it
manages reads it at startup; `make enable-physical-limiter` restarts those). The installer writes that list for you: `WVA_LIMITER=quota` with
`WVA_QUOTAS='H200=8'` for one install's own policy, `WVA_LIMITER_TYPE` with the
same `WVA_QUOTAS` for the cluster policy (read by the controllers listed in
[the guide](../../guides/admin-gpu-bounding/#installation-instructions)). The quota form
**requires** a budget; an entry naming no accelerator is a budget of zero, not
unlimited. Where Kueue already holds the numbers, `WVA_QUOTA_KUEUE=true` reads
them from its ClusterQueues instead of repeating them here:
[bound tenants by the quotas Kueue already holds](../kueue-bounded-quotas/)
(experimental).
Schema, scopes and validation: [quota limiter](../../reference/quota-limiter.md).
Where the policy lives and who may edit it:
[the GPU limiter](../../reference/gpu-limiter.md).
