# Bound a fleet by real GPUs

Stops the scaling manager from asking for more GPUs than the cluster has. For
any shared cluster: where more than one team scales on the same accelerators,
or where `maxReplicaCount` is a safety limit rather than a real ceiling.

**Maturity:** Stable.

## When not to take it

- You need enforcement. This limiter is advisory: it stops the controller
  *asking* for replicas that cannot be placed. The boundary that holds is a
  GPU `ResourceQuota` ([bound at admission](../../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).
  Use both.
- Your question is "is this tenant within its allowance?", not "is the
  hardware there?". Take [cap what each tenant may take](../tenant-gpu-quotas/).

## Start here

You need a cluster admin, and controllers installed by
[the saturation path](../scale-on-saturation/#start-here). Every step is in
[Bound every install by real GPUs](../../guides/admin-gpu-bounding/).

1. Cluster admin: [check accelerators resolve](../../guides/admin-gpu-bounding/README.md#prerequisites). If they do not, the failure has no symptom.
2. Cluster admin: run [`make enable-physical-limiter`](../../guides/admin-gpu-bounding/README.md#installation-instructions). Read the list it prints first: some controllers will be restarted ([details](#what-the-command-restarts)).
3. [Check it works](../../guides/admin-gpu-bounding/README.md#verification): each controller reports the limiter `inventory/<name>`, not `none`.

To undo, the admin runs `make disable-physical-limiter`
([Cleanup](../../guides/admin-gpu-bounding/README.md#cleanup)). It removes only
the physical limiter; a quota in the same policy stays. To bound one
install instead, its namespace owner adds `WVA_LIMITER=gpu-inventory` to
`make deploy-wva` ([configuration](../../reference/configuration.md#core-configuration));
the same owner can edit it away.

## What it costs

- No accelerators.
- Latency when the budget binds: a model held at its budget queues what an
  extra replica would have served. That shows up as TTFT (time to first
  token), not as an error.
- An owner: "why did we not scale" becomes a declared policy somebody answers for.

## Evidence

No benchmark run. The end-to-end suites under
[How it is tested](#how-it-is-tested) cover it.

## Details

### What it is

Without a limiter, each controller scales its workloads up to their
`maxReplicaCount`, and nothing checks that against the accelerators that
exist. A limiter gives the optimizer a budget: the GPU inventory discovered on
the cluster (`gpu-inventory`), or per-accelerator caps an operator declares
(`quota`). A limiter without a quota bounds a well-behaved autoscaler, not the
cluster.

### What it needs

- Cluster-admin rights, once. The policy must live where a tenant cannot edit
  it, and every controller that follows it must be able to read it.
- Every controller able to **list nodes**. A controller that cannot resolve a
  variant's accelerator gives it no budget and never scales it up. That is why
  the guide checks `reason=AcceleratorNotResolved` first.
- On **Kubernetes**, a namespace-scoped controller needs `get` on its own
  Namespace, which `make setup-prereqs` grants ([older installs](#details-older-installs)).

### What the command restarts

`make enable-physical-limiter` restarts every namespace-scoped controller whose
policy lives outside the namespace it manages (marked `WILL BE RESTARTED`), on
this run and every later one. Leave a namespace out of `WVA_LIMITER_TARGETS`
where that restart is not acceptable now. A later `make deploy-wva` without
`WVA_LIMITER` keeps the limiter and says so; `WVA_LIMITER=none` removes it
([why](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).
`make disable-physical-limiter` leaves the node-read RBAC in place.

### Which limiter

**You can declare both.** Every limiter in the `limiters:` list is built, and a
model must fit all of them. Enabling one kind keeps the other. Declare both when
the allowance can exceed the GPUs that exist, for example a Kueue quota on a
shared cluster.

| | `gpu-inventory` | `quota` |
| --- | --- | --- |
| Question | is the hardware there? | is this tenant within its allowance? |
| Counts | every GPU-holding Pod on the cluster | only what **the scaling manager's own variants** hold |
| Declared | nothing to declare: discovered | per accelerator type, at cluster or namespace scope |

The shipped policy declares no limiter. `gpu-inventory` is only the default of
`WVA_LIMITER_TYPE`, which `make enable-physical-limiter` reads. What a quota
counts, why a cap is a ceiling and not a reservation, and why a missing entry
denies are in [capping what each tenant may take](../tenant-gpu-quotas/).

### Verifying it

The guide's [Verification](../../guides/admin-gpu-bounding/README.md#verification)
has the commands. Read them like this:

- `GPU limiter (re)built from config` with `type` `inventory` (after a restart,
  `GPU limiter constructed` with that `type`), and no `Invalid saturation
  scaling`. `GPU limiter constructed` alone proves nothing: every controller
  prints it at start, `type` `none` included.
- `wva_model_scaling_blocked` carries the reason as a label. A model that
  wanted more and did not get it is a normal, visible outcome here.
- `scaling-decision` lines show what the optimizer decided, with a reason code
  per entry.

Admin-owned controllers read their own ConfigMap by design; see
[the GPU limiter](../../reference/gpu-limiter.md#what-an-admin-sets-and-where).

### How it is tested

- End-to-end: `test/e2e/limiter_test.go` (labelled `full`), and
  `test/e2e/warm_pool_quota_test.go` for the refusal when a pool asks for
  accelerators the quota will not give.
- The accounting, and three ways a GPU budget can over-state free capacity,
  are in [gpu-capacity-accounting.md](../../concepts/gpu-capacity-accounting.md).

### Tuning it

The controller reads only the `limiters:` list on the scaling-policy
ConfigMap's `default` entry. It is dynamic, so the engine rebuilds it without a
restart. The exception is a namespace-scoped controller reading it from
outside its namespace, which reads it at startup; `make enable-physical-limiter`
restarts those.

The installer writes that list: `WVA_LIMITER=quota` with `WVA_QUOTAS='H200=8'`
for one install's own policy, and `WVA_LIMITER_TYPE` with the same
`WVA_QUOTAS` for the cluster policy (read by the controllers listed in
[the guide](../../guides/admin-gpu-bounding/#installation-instructions)). The
quota form **requires** a budget: an entry naming no accelerator is a budget
of zero, not unlimited. Where Kueue already holds the numbers,
`WVA_QUOTA_KUEUE=true` reads them from its ClusterQueues:
[bound tenants by the quotas Kueue already holds](../kueue-bounded-quotas/)
(experimental). Schema and validation: [quota limiter](../../reference/quota-limiter.md).
Where the policy lives and who may edit it: [the GPU limiter](../../reference/gpu-limiter.md).

### Details: older installs

A namespace-scoped controller set up before `make setup-prereqs` granted `get`
on its Namespace (`wva-policy-namespace-reader`) does not see `wva-policy`. It
keeps reading its own ConfigMap and says so: `Cannot tell whether a cluster
admin published policy` or `Could not read the managed Namespace` in its log,
`wva_policy_namespace_unverified` = 1, and alert
`WVAPolicyNamespaceUnverified`. A cluster admin re-runs
`NAMESPACE=<ns> make setup-prereqs`, or `make enable-physical-limiter`, which
also restarts it ([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-to-this-release);
[grant it by hand](../../guides/admin-cluster-setup/README.md#grant-the-namespace-read-by-hand)
only where neither can run).
